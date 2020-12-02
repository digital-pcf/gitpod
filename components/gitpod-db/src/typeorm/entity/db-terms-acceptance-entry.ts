/**
 * Copyright (c) 2020 TypeFox GmbH. All rights reserved.
 * Licensed under the GNU Affero General Public License (AGPL).
 * See License-AGPL.txt in the project root for license information.
 */

import { Column, Entity, PrimaryColumn } from "typeorm";
import { TermsAcceptanceEntry } from "@gitpod/gitpod-protocol";

@Entity()
export class DBTermsAcceptanceEntry implements TermsAcceptanceEntry {

    @PrimaryColumn()
    userId: string;

    @Column()
    termsRevision: string;

    @Column()
    acceptionTime: string;
}
